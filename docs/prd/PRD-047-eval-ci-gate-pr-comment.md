# PRD-047: Eval CI Gate with PR Comment Integration (`tag eval ci`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (3-5 days)
**Category:** Evaluation & Observability
**Affects:** `controller.py (cmd_eval_ci) + ci.py`
**Depends on:** PRD-027 (eval framework), PRD-020 (CI/CD integration), PRD-013 (agent tracing/observability), PRD-034 (secret scanning), PRD-028 (sandbox code execution)
**Inspired by:** Braintrust GitHub Action, LangSmith CI/CD
**GitHub Issue:** #343

---

## 1. Overview

TAG's eval framework (PRD-027) gives engineers a YAML-driven regression-testing harness for agent behavior, storing results in SQLite and surfacing trends over time. But running `tag eval run` locally before merging is optional — nothing in the development workflow enforces it. Teams that forget to run evals, or run them against the wrong profile version, ship regressions to production with no automated safety net. The gap between "we have evals" and "evals actually gate merges" is the problem this PRD closes.

`tag eval ci` is a purpose-built CI entrypoint that wires the existing eval machinery directly into GitHub pull request workflows. When added to a GitHub Actions job, it runs a named eval suite against the profile being modified, fails the CI job (non-zero exit) when the aggregate pass rate drops below a configurable threshold (`--fail-below`), and posts the full result table as a PR comment so reviewers can see quality signal alongside code changes — without leaving GitHub. The implementation reuses `post_pr_comment` and `post_pr_review_comments` from the existing `ci.py` module and the `eval_framework.py` scoring engine from PRD-027, adding only a thin orchestration layer in `cmd_eval_ci` inside `controller.py` and a new `src/tag/ci_eval.py` module for the CI-specific logic.

Beyond the immediate gating behavior, the feature includes two supporting commands: `tag ci install-action --type eval` scaffolds a ready-to-use `.github/workflows/tag-eval.yml` file so teams can add eval gating in under two minutes, and `tag eval dataset create / list` builds a first-class dataset management layer. Datasets are named, versioned collections of (input, expected_output) pairs — seeded from real production runs, exported as YAML, and consumed by eval suites. This closes the feedback loop: production traces → curated dataset → eval suite → CI gate → blocked regression.

The design draws directly from Braintrust's GitHub Action pattern (threshold-based gate with PR summary) and LangSmith CI/CD's approach (evaluate() returning structured results that feed into a pass/fail check), adapted for TAG's local-first, SQLite-backed architecture. No external eval service is required; everything runs inside the GitHub Actions runner using the `tag-agent` CLI already available via `pip install tag-agent`.

---

## 2. Problem Statement

### 2.1 Evals are voluntary and invisible in code review

PRD-027 makes it easy to *run* evals, but the result lives in a local SQLite database that reviewers cannot see. A PR author may skip `tag eval run` entirely, or run it on a stale local profile, and the reviewer has no way to know. Without machine-enforced eval gating, regressions slip through code review because quality signal is absent from the only review surface that matters: the pull request itself. Braintrust's GitHub Action and LangSmith's CI integration exist precisely because "run evals before merging" as a social norm is not reliable at team scale.

### 2.2 The CI integration gap is high-friction to close manually

A team that wants to add eval gating today must write a custom GitHub Actions workflow that: installs TAG, loads the profile, parses `tag eval run --json` output, computes a pass rate, compares it to a threshold, formats a Markdown comment, and calls `gh pr comment`. Each of these steps is non-trivial and requires GitHub Actions expertise that most ML/agent engineers lack. The result is that eval gating stays a backlog item. `tag eval ci` removes all of this custom glue by providing a single command that does all steps, and `tag ci install-action --type eval` removes even the workflow YAML authoring step.

### 2.3 Datasets lack a first-class management layer

PRD-027's `tag eval add --from-run` lets engineers grow a suite from production runs one case at a time, but there is no concept of a named, versioned *dataset* that can be shared across suites, exported, or rebuilt from a time-range query. Teams end up with suites that have hard-coded `input` strings that drift away from what real users actually submit. `tag eval dataset create --from-runs --since 7d --limit 50` closes this gap: it mines recent production runs from the `runs` + `steps` tables, deduplicates them, and produces a versioned dataset YAML ready for use as eval suite inputs.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag eval ci --suite evals/golden.yaml --fail-below 0.85` exits non-zero in CI when the eval pass rate is below the threshold, blocking the merge. |
| G2 | `--post-comment --repo owner/repo --pr $PR_NUMBER` posts a rich Markdown eval result table as a PR comment using the existing `post_pr_comment` function in `ci.py`. |
| G3 | `tag ci install-action --type eval` scaffolds `.github/workflows/tag-eval.yml` with a ready-to-use GitHub Actions workflow in under 30 seconds. |
| G4 | `tag eval dataset create my-golden --from-runs --since 7d --limit 50` mines the `runs`/`steps` tables and produces a named, versioned dataset YAML. |
| G5 | `tag eval dataset list --json` lists all known datasets with row counts, source info, and last-modified timestamps. |
| G6 | All eval CI results are persisted to a new `eval_ci_runs` SQLite table for auditing and trend analysis. |
| G7 | Zero new required dependencies for the CI gate path; the feature works with the packages already installed by `pip install tag-agent`. |
| G8 | The PR comment format is machine-parseable: a hidden HTML comment embeds the JSON result so future tooling can extract it without re-running evals. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing the existing `tag eval run` command. `tag eval ci` is a thin CI-optimized wrapper; the full eval framework from PRD-027 remains unchanged. |
| NG2 | Supporting GitLab MR comments or Bitbucket PR comments in this PRD. Only GitHub PRs via the `gh` CLI are in scope. |
| NG3 | LLM-as-judge scoring in the CI gate path. `tag eval ci` uses the deterministic keyword/pattern scoring already in `eval_framework.score_case()`. Adding DeepEval judge calls in CI is explicitly deferred (too slow and too costly for routine CI). |
| NG4 | Automatic profile rollback on regression. Eval CI detects and reports; it does not revert profile changes. |
| NG5 | Real-time streaming of eval progress to the PR. The PR comment is posted once, after all cases complete. |
| NG6 | Multi-suite aggregation in a single `tag eval ci` invocation. Each invocation targets exactly one suite. Teams that want to gate on multiple suites add multiple workflow steps. |
| NG7 | Dataset versioning with content-addressable hashes or a remote registry. Datasets are local YAML files; versioning is via the `version` field in the YAML header. |

---

## 4. Success Metrics

| Metric | Target | How Measured |
|--------|--------|-------------|
| Time to add eval CI gate to a repo | Under 2 minutes from zero | Manual timing: `tag ci install-action --type eval` + commit + push |
| PR comment post success rate | >= 99% when `gh` CLI is authenticated | CI integration test against real GitHub sandbox repo |
| False-positive gate failures (flaky evals) | < 2% of CI runs | Monitor `eval_ci_runs` table: flag runs where re-running produces a different pass/fail decision |
| Dataset creation time for 50 cases from 7 days of runs | Under 5 seconds | `time tag eval dataset create test --from-runs --since 7d --limit 50` on 10K-row `runs` table |
| Threshold accuracy | `tag eval ci --fail-below 0.85` exits 1 iff pass_rate < 0.85 | Parameterized unit test at boundary values |
| CLI latency for `tag eval dataset list` | Under 200ms | Benchmark with 100 datasets in `eval_datasets` table |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | run `tag ci install-action --type eval` once | My team gets a working GitHub Actions eval gate in one command, without writing YAML manually |
| U2 | Profile author | see the eval result table directly in the GitHub PR I opened | I don't have to go to a separate dashboard or run evals locally to see how my profile change performed |
| U3 | Reviewer | see a PR comment showing pass rate 72% against a threshold of 85% | I can block the merge without running the suite myself, with confidence the gate is automated and consistent |
| U4 | DevOps engineer | set `--fail-below 0.85` in the workflow | CI automatically fails and blocks the merge whenever eval quality drops, with no manual intervention |
| U5 | ML engineer | run `tag eval dataset create my-golden --from-runs --since 7d --limit 50` | I get a curated eval dataset from recent production traffic, removing the need to hand-craft test cases |
| U6 | Team lead | run `tag eval dataset list --json` | I can see all datasets in the repo and their row counts in a format I can pipe to other tools |
| U7 | Developer | add `--suite evals/golden.yaml --fail-below 0.85 --post-comment --repo owner/repo --pr $PR_NUMBER` to an existing CI job | I get PR comment integration without adding a new workflow file |
| U8 | Security engineer | confirm that the `GH_TOKEN` used in the eval workflow has only `pull-requests: write` and `contents: read` permissions | I know the eval gate does not have write access to code or secrets |

---

## 6. Proposed CLI Surface

### 6.1 `tag eval ci`

The primary CI gate command. Designed to be invoked inside a GitHub Actions `run:` step.

```bash
tag eval ci \
  --suite evals/golden.yaml \
  --fail-below 0.85 \
  [--post-comment] \
  [--repo owner/repo] \
  [--pr $PR_NUMBER] \
  [--profile coder] \
  [--json] \
  [--quiet] \
  [--timeout 300]
```

**Flags:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--suite` | path | required | Path to eval suite YAML. Resolved relative to cwd, then `~/.tag/evals/`. |
| `--fail-below` | float | `0.0` (never fail) | Fail (exit 1) when aggregate pass rate < this value. Range: 0.0–1.0. |
| `--post-comment` | bool flag | false | Post results as a PR comment. Requires `--repo` and `--pr`. |
| `--repo` | string | — | GitHub repository in `owner/name` format. |
| `--pr` | int | — | Pull request number. Accepts `$PR_NUMBER` env expansion. |
| `--profile` | string | suite YAML `profile` field, then config default | TAG profile to run cases against. |
| `--json` | bool flag | false | Emit results as JSON to stdout in addition to the human-readable summary. |
| `--quiet` | bool flag | false | Suppress per-case progress lines; only print final summary. |
| `--timeout` | int | 300 | Per-case agent run timeout in seconds. |

**Exit codes:**

| Code | Meaning |
|------|---------|
| `0` | All cases ran; pass rate >= `--fail-below` threshold. |
| `1` | Pass rate < `--fail-below` threshold (the gate triggered). |
| `2` | Internal error: bad YAML, missing profile, SQLite error, subprocess crash. |
| `3` | Partial failure: some cases timed out or errored; gate decision is based on cases that did complete. |

**Example — full CI run with PR comment:**

```bash
tag eval ci \
  --suite evals/golden.yaml \
  --fail-below 0.85 \
  --post-comment \
  --repo acme-inc/backend \
  --pr 423 \
  --profile coder
```

**stdout output (TTY):**

```
Eval CI: evals/golden.yaml  profile=coder  threshold=0.85
Running 12 cases...
  [✓] write-fibonacci          score=0.92
  [✓] fix-off-by-one           score=0.88
  [✗] refactor-dataclass       score=0.61  reason: missing type annotations
  [✓] explain-generator        score=0.91
  ... (8 more)

Results: 10/12 passed  pass_rate=0.833  threshold=0.850
GATE: FAIL — pass rate 83.3% is below threshold 85.0%
Posted eval results to acme-inc/backend#423
```

**Exit code:** `1`

**PR comment (posted via `post_pr_comment`):**

```markdown
## TAG Eval CI Results

| Suite | Cases | Passed | Failed | Pass Rate | Threshold | Status |
|-------|-------|--------|--------|-----------|-----------|--------|
| evals/golden.yaml | 12 | 10 | 2 | 83.3% | 85.0% | ❌ FAIL |

### Failed Cases

| Case | Score | Reason |
|------|-------|--------|
| refactor-dataclass | 0.61 | missing type annotations |
| summarize-long-text | 0.58 | output exceeded max_length |

### All Cases

| Case | Score | Threshold | Status |
|------|-------|-----------|--------|
| write-fibonacci | 0.92 | 0.80 | ✅ pass |
| fix-off-by-one | 0.88 | 0.75 | ✅ pass |
| refactor-dataclass | 0.61 | 0.75 | ❌ fail |
| ... | | | |

---
*Generated by [tag-agent](https://github.com/tag-agent/tag) `tag eval ci` · eval_ci_run_id: `ecr_7f3a9b2c`*

<!-- tag-eval-result: {"eval_ci_run_id":"ecr_7f3a9b2c","suite":"evals/golden.yaml","profile":"coder","pass_rate":0.833,"threshold":0.85,"passed":10,"failed":2,"total":12,"gate":"FAIL"} -->
```

**`--json` stdout output:**

```json
{
  "eval_ci_run_id": "ecr_7f3a9b2c",
  "suite": "evals/golden.yaml",
  "profile": "coder",
  "pass_rate": 0.8333,
  "threshold": 0.85,
  "passed": 10,
  "failed": 2,
  "total": 12,
  "gate": "FAIL",
  "cases": [
    {
      "case_id": "write-fibonacci",
      "score": 0.92,
      "passed": true,
      "threshold": 0.80,
      "failure_reason": null
    },
    {
      "case_id": "refactor-dataclass",
      "score": 0.61,
      "passed": false,
      "threshold": 0.75,
      "failure_reason": "missing type annotations"
    }
  ],
  "comment_posted": true,
  "pr": 423,
  "repo": "acme-inc/backend"
}
```

---

### 6.2 `tag ci install-action --type eval`

Scaffolds a GitHub Actions workflow for eval CI gating.

```bash
tag ci install-action --type eval \
  [--suite evals/golden.yaml] \
  [--fail-below 0.85] \
  [--profile coder] \
  [--output .github/workflows/tag-eval.yml] \
  [--force]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--suite` | `evals/golden.yaml` | Suite path embedded in the workflow `run:` step. |
| `--fail-below` | `0.85` | Threshold value written into the workflow. |
| `--profile` | `coder` | Profile name written into the workflow. |
| `--output` | `.github/workflows/tag-eval.yml` | Output path. Relative to cwd. |
| `--force` | false | Overwrite existing file without prompting. |

**stdout on success:**

```
Scaffolded: .github/workflows/tag-eval.yml
Commit and push to enable eval CI gating on every PR.

Next steps:
  1. Add your TAG API key as a GitHub secret: GH_TOKEN (already required by gh CLI)
  2. Optionally customize --fail-below and --suite in the workflow file
  3. git add .github/workflows/tag-eval.yml && git commit -m "ci: add TAG eval gate"
```

**Generated `.github/workflows/tag-eval.yml`:**

```yaml
# Generated by: tag ci install-action --type eval
# Docs: https://github.com/tag-agent/tag/blob/main/docs/prd/PRD-047-eval-ci-gate-pr-comment.md
name: TAG Eval CI Gate

on:
  pull_request:
    branches: [main, master]

permissions:
  contents: read
  pull-requests: write

jobs:
  eval-gate:
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v4

      - name: Set up Python
        uses: actions/setup-python@v5
        with:
          python-version: "3.11"

      - name: Install tag-agent
        run: pip install tag-agent

      - name: Run eval CI gate
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          ANTHROPIC_API_KEY: ${{ secrets.ANTHROPIC_API_KEY }}
        run: |
          tag eval ci \
            --suite evals/golden.yaml \
            --fail-below 0.85 \
            --profile coder \
            --post-comment \
            --repo ${{ github.repository }} \
            --pr ${{ github.event.pull_request.number }}
```

---

### 6.3 `tag eval dataset create`

Build a named dataset from recent production runs.

```bash
tag eval dataset create my-golden \
  --from-runs \
  --since 7d \
  [--limit 50] \
  [--profile coder] \
  [--output evals/datasets/my-golden.yaml] \
  [--min-output-length 20] \
  [--dedupe] \
  [--json]
```

**Flags:**

| Flag | Default | Description |
|------|---------|-------------|
| `--from-runs` | required | Seed dataset from the `runs`/`steps` SQLite tables. |
| `--since` | required | Time window: `7d`, `24h`, `30d`, etc. Parsed to a `timedelta`. |
| `--limit` | 50 | Maximum number of cases to include. |
| `--profile` | (all) | Filter runs to only those with `master_profile = X`. |
| `--output` | `evals/datasets/<name>.yaml` | Output path for the dataset YAML. |
| `--min-output-length` | 10 | Exclude runs where agent output has fewer than N characters (filters low-quality runs). |
| `--dedupe` | true | Remove near-duplicate inputs using a simple token-overlap similarity check (Jaccard >= 0.85 = duplicate). |
| `--json` | false | Print created dataset metadata as JSON instead of human summary. |

**stdout:**

```
Dataset: my-golden
Source: runs table  since=7d  profile=coder
Candidates found: 183
After deduplication: 61
After limit: 50
Written to: evals/datasets/my-golden.yaml

Register in a suite with:
  tag eval dataset list
  # then add to evals/golden.yaml:
  #   dataset: evals/datasets/my-golden.yaml
```

**Generated `evals/datasets/my-golden.yaml`:**

```yaml
# TAG Eval Dataset — generated by: tag eval dataset create my-golden --from-runs --since 7d --limit 50
name: my-golden
version: "1.0.0"
created_at: "2026-06-17T10:42:00Z"
source:
  type: runs_table
  since: "7d"
  profile: coder
  dedupe: true
  min_output_length: 20
row_count: 50

rows:
  - id: row_001
    run_id: run_abc123          # source run for traceability
    input: "Write a Python function that returns the nth Fibonacci number"
    reference_output: |
      def fibonacci(n):
          a, b = 0, 1
          for _ in range(n):
              a, b = b, a + b
          return a
    created_at: "2026-06-14T08:23:15Z"

  - id: row_002
    run_id: run_def456
    input: "Fix the off-by-one error in this range loop"
    reference_output: "Change range(n) to range(n+1) to include the endpoint."
    created_at: "2026-06-15T14:07:33Z"

  # ... 48 more rows
```

---

### 6.4 `tag eval dataset list`

List all known datasets.

```bash
tag eval dataset list [--json] [--profile coder]
```

**stdout (TTY):**

```
DATASET           ROWS  PROFILE  CREATED              SOURCE
my-golden           50  coder    2026-06-17T10:42:00  runs_table (7d)
smoke-tests         12  (any)    2026-06-01T09:00:00  manual
regression-q2      100  writer   2026-05-30T11:22:00  runs_table (30d)
```

**`--json` output:**

```json
[
  {
    "name": "my-golden",
    "path": "evals/datasets/my-golden.yaml",
    "row_count": 50,
    "profile": "coder",
    "created_at": "2026-06-17T10:42:00Z",
    "source_type": "runs_table",
    "version": "1.0.0"
  }
]
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag eval ci` loads and validates the suite YAML using `eval_framework.load_suite()`. If validation fails, it exits 2 with a descriptive error including the YAML field path that failed. |
| FR-02 | For each case in the suite, `tag eval ci` invokes the agent (via the same subprocess path as `cmd_eval` in PRD-027: `tag submit`) and captures the agent's output. Timeouts are enforced per-case via the `--timeout` flag; a timed-out case is recorded as `passed=False, score=0.0, failure_reason="timeout"`. |
| FR-03 | Scoring uses `eval_framework.score_case(case, output)` (deterministic keyword/pattern scoring). `tag eval ci` explicitly does not invoke DeepEval LLM-as-judge in the CI path (see NG3). |
| FR-04 | After all cases complete, `tag eval ci` computes `pass_rate = passed_count / total_count` where `total_count` includes timed-out cases (counted as failures). Partial-error runs (exit code 3) compute pass rate over only the cases that completed. |
| FR-05 | The gate decision: if `pass_rate < fail_below`, exit 1. If `pass_rate >= fail_below`, exit 0 (or exit 3 for partial errors). A `--fail-below` of `0.0` (the default) means the gate never triggers on threshold — useful for comment-only mode without blocking. |
| FR-06 | When `--post-comment` is provided with `--repo` and `--pr`, `tag eval ci` calls `ci.post_pr_comment(repo, pr_number, body)` with the Markdown body defined in Section 6.1. If `post_pr_comment` returns `False`, `tag eval ci` prints a warning to stderr but does not change the exit code (the gate decision is independent of comment success). |
| FR-07 | The PR comment body includes a hidden HTML comment on the final line containing the JSON result blob (see Section 6.1). This enables downstream tooling to extract structured results from PR comment text via `gh pr view --json comments`. |
| FR-08 | Every `tag eval ci` run writes one row to the `eval_ci_runs` table and one row per case to the `eval_ci_cases` table (DDL in Section 9.3). Writes happen even on gate failure. If the database write fails (e.g., SQLite locked for >5s), `tag eval ci` logs the error to stderr but proceeds with the gate decision. |
| FR-09 | `tag ci install-action --type eval` creates `.github/workflows/tag-eval.yml` (or the `--output` path) with the content exactly as defined in Section 6.2. If the file already exists and `--force` is not passed, exit 2 with the message: `"File already exists: <path>. Use --force to overwrite."` |
| FR-10 | `tag eval dataset create <name> --from-runs --since <window> --limit N` queries the `runs` table for rows with `created_at >= now - window AND status = 'done'`, joins to `steps` to get the last assistant output, applies `--min-output-length` filter, deduplicates by Jaccard similarity, and writes a dataset YAML to `--output` (defaulting to `evals/datasets/<name>.yaml`). The `evals/datasets/` directory is created if it does not exist. |
| FR-11 | Deduplication in `tag eval dataset create` uses Jaccard similarity on token sets (splitting on whitespace and punctuation). Any two inputs with Jaccard similarity >= 0.85 are considered duplicates; only the earlier-created run is kept. The deduplication threshold is hardcoded at 0.85 (not yet configurable). |
| FR-12 | `tag eval dataset list` discovers datasets by scanning `evals/datasets/*.yaml` relative to cwd, and `~/.tag/datasets/*.yaml`. It reads the `name`, `version`, `created_at`, `row_count`, `source.type`, and `source.profile` fields from each YAML header. It does not load the full `rows` list (for performance with large datasets). |
| FR-13 | The `eval_datasets` SQLite table (DDL in Section 9.3) is a lightweight index of known datasets, updated by `tag eval dataset create` and `tag eval dataset list`. It is used by `tag eval dataset list --json` without re-scanning the filesystem. |
| FR-14 | When `--json` is passed to `tag eval ci`, the JSON output is written to stdout. The human-readable summary is suppressed unless stderr is a TTY. When `--quiet` is also passed, per-case lines are suppressed from both stdout and stderr. |
| FR-15 | The `--since` flag for `tag eval dataset create` accepts the formats: `Nd` (N days), `Nh` (N hours), `Nw` (N weeks). Invalid format exits 2 with: `"Invalid --since format: <value>. Use Nd, Nh, or Nw (e.g. 7d, 24h, 2w)."` |
| FR-16 | `tag eval ci` reads `ANTHROPIC_API_KEY` (or other model-provider keys) from the profile's resolved environment via `profile_exec_env(cfg, profile)`. It does not require a separate environment variable for the CI gate itself. |
| FR-17 | The generated `tag-eval.yml` uses `permissions: pull-requests: write, contents: read` at the job level and no other permissions. `tag ci install-action` does not emit a workflow that uses `GITHUB_TOKEN` with write access to code, packages, or secrets. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **CI execution time:** `tag eval ci` wall time for a 50-case suite must not exceed `50 * (per_case_timeout + 2s overhead)`. The 2s overhead per case accounts for subprocess spawn, SQLite write, and output parsing. For a 300s timeout and 50 cases, worst-case is 4,266s; but typical TAG agent runs complete in 10–30s, making a 50-case suite finish in under 25 minutes — acceptable for a PR gate. |
| NFR-02 | **Idempotent PR comments:** If `tag eval ci --post-comment` is run multiple times on the same PR (e.g., re-triggered CI), it posts a new top-level comment each time. It does not attempt to edit/replace the previous comment. The `eval_ci_run_id` in each comment allows humans to identify which run produced which comment. A future PRD may add comment-update behavior via `gh api --method PATCH`. |
| NFR-03 | **No network calls when `--post-comment` is not set:** In gate-only mode (no `--post-comment`), `tag eval ci` makes zero outbound HTTP calls itself. Agent subprocess runs may make model API calls depending on the profile, but the gate orchestrator itself is network-free. |
| NFR-04 | **Graceful `gh` CLI absence:** If `--post-comment` is set but `gh` is not installed or not authenticated, `tag eval ci` prints a clear error to stderr: `"gh CLI not found or not authenticated. Install gh and run 'gh auth login'."` and proceeds with the gate decision (the exit code reflects only threshold, not comment success). |
| NFR-05 | **Thread-safe SQLite writes:** `eval_ci_cases` rows are written inside a `BEGIN IMMEDIATE` transaction per case. Concurrent `tag eval ci` runs on different suites on the same runner do not deadlock because WAL mode is already enabled by `open_db()`. |
| NFR-06 | **`--json` output is machine-parseable:** The JSON schema for `tag eval ci --json` is stable and versioned. Any breaking change to the JSON schema requires a new `schema_version` field increment and a deprecation notice. |
| NFR-07 | **Dataset YAML reproducibility:** `tag eval dataset create` with the same `--since`, `--limit`, and `--profile` flags applied within the same 1-second window must produce the same output (deterministic sort by `created_at ASC, run_id ASC` before applying `--limit`). |
| NFR-08 | **`tag eval ci` is safe to run on the main branch:** When `--pr` is not provided, `--post-comment` is a no-op even if specified (with a warning). This prevents accidental comment posting on non-PR runs. |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/ci_eval.py` | CI-specific eval orchestration: `run_eval_ci()`, `build_pr_comment_body()`, `format_results_json()`, `parse_since_window()`, `create_dataset_from_runs()`, `list_datasets()`. Imports from `eval_framework.py` and `ci.py`. |
| `.github/workflows/tag-eval.yml` | Generated by `tag ci install-action --type eval`. Not committed to the tag-agent repo itself; scaffolded into user repos. |

### 9.2 Modified Files

| File | Changes |
|------|---------|
| `src/tag/controller.py` | Add `cmd_eval_ci(args)` (handles `tag eval ci`), `cmd_eval_dataset(args)` (handles `tag eval dataset create/list`), extend `cmd_ci(args)` to handle `install-action` subcommand, wire new argparse subparsers. |
| `src/tag/ci.py` | No changes to existing functions. Consumed as-is by `ci_eval.py`. |

### 9.3 SQLite DDL

```sql
-- Table for eval CI run metadata (one row per `tag eval ci` invocation)
CREATE TABLE IF NOT EXISTS eval_ci_runs (
  id               TEXT PRIMARY KEY,          -- "ecr_" + uuid4 hex prefix
  suite_path       TEXT NOT NULL,             -- path of the .yaml file as passed (not normalized)
  suite_name       TEXT NOT NULL,             -- name field from YAML
  profile          TEXT NOT NULL,             -- TAG profile used
  threshold        REAL NOT NULL,             -- --fail-below value (0.0 if not set)
  pass_rate        REAL,                      -- final computed pass rate; NULL if all cases errored
  passed_count     INTEGER NOT NULL DEFAULT 0,
  failed_count     INTEGER NOT NULL DEFAULT 0,
  total_count      INTEGER NOT NULL DEFAULT 0,
  gate_result      TEXT NOT NULL,             -- 'PASS' | 'FAIL' | 'ERROR' | 'PARTIAL'
  comment_posted   INTEGER NOT NULL DEFAULT 0,-- 1 if PR comment was posted successfully
  pr_number        INTEGER,                   -- PR number; NULL if not run in PR context
  repo             TEXT,                      -- 'owner/name'; NULL if not run in PR context
  exit_code        INTEGER NOT NULL DEFAULT 0,
  created_at       TEXT NOT NULL,             -- ISO-8601 UTC
  completed_at     TEXT                       -- ISO-8601 UTC; NULL if run is still in progress
);

CREATE INDEX IF NOT EXISTS idx_ecr_suite_profile
  ON eval_ci_runs(suite_path, profile, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_ecr_gate
  ON eval_ci_runs(gate_result, created_at DESC);


-- Table for per-case results within an eval CI run
CREATE TABLE IF NOT EXISTS eval_ci_cases (
  id               TEXT PRIMARY KEY,          -- uuid4
  eval_ci_run_id   TEXT NOT NULL,             -- FK to eval_ci_runs.id
  case_id          TEXT NOT NULL,             -- case.id from suite YAML
  input            TEXT NOT NULL,             -- case.input (prompt sent to agent)
  output           TEXT NOT NULL DEFAULT '',  -- agent output captured
  score            REAL NOT NULL DEFAULT 0.0, -- 0.0–1.0 from score_case()
  passed           INTEGER NOT NULL DEFAULT 0,-- 1 if passed, 0 if failed/timeout/error
  threshold        REAL NOT NULL DEFAULT 0.0, -- effective threshold for this case
  failure_reason   TEXT,                      -- reason string from score_case(); NULL if passed
  error            TEXT,                      -- exception/timeout message if agent run failed
  duration_ms      INTEGER NOT NULL DEFAULT 0,-- wall time for this case's agent run
  created_at       TEXT NOT NULL,
  FOREIGN KEY(eval_ci_run_id) REFERENCES eval_ci_runs(id)
);

CREATE INDEX IF NOT EXISTS idx_ecc_run
  ON eval_ci_cases(eval_ci_run_id, passed);


-- Lightweight index of known datasets (updated by dataset create / dataset list)
CREATE TABLE IF NOT EXISTS eval_datasets (
  name             TEXT PRIMARY KEY,          -- dataset name (unique within this DB)
  path             TEXT NOT NULL,             -- filesystem path to the YAML file
  version          TEXT NOT NULL DEFAULT '1.0.0',
  row_count        INTEGER NOT NULL DEFAULT 0,
  source_type      TEXT NOT NULL,             -- 'runs_table' | 'manual'
  source_profile   TEXT,                      -- profile filter used at creation; NULL = all profiles
  source_since     TEXT,                      -- since window string e.g. '7d'; NULL if manual
  created_at       TEXT NOT NULL,
  updated_at       TEXT NOT NULL
);
```

### 9.4 Core Dataclasses

```python
# src/tag/ci_eval.py
from __future__ import annotations

import datetime
import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any


@dataclass
class CIEvalCaseResult:
    """Result for a single eval case in CI context."""
    case_id: str
    input: str
    output: str
    score: float          # 0.0–1.0 from score_case()
    passed: bool
    threshold: float
    failure_reason: str | None
    error: str | None     # set if agent subprocess failed or timed out
    duration_ms: int


@dataclass
class CIEvalRunResult:
    """Aggregate result for a full `tag eval ci` invocation."""
    eval_ci_run_id: str
    suite_path: str
    suite_name: str
    profile: str
    threshold: float
    cases: list[CIEvalCaseResult] = field(default_factory=list)
    comment_posted: bool = False
    pr_number: int | None = None
    repo: str | None = None

    @property
    def passed_count(self) -> int:
        return sum(1 for c in self.cases if c.passed)

    @property
    def failed_count(self) -> int:
        return sum(1 for c in self.cases if not c.passed)

    @property
    def total_count(self) -> int:
        return len(self.cases)

    @property
    def pass_rate(self) -> float:
        if self.total_count == 0:
            return 0.0
        return self.passed_count / self.total_count

    @property
    def gate_result(self) -> str:
        """'PASS' | 'FAIL' | 'ERROR' | 'PARTIAL'"""
        if self.total_count == 0:
            return "ERROR"
        error_cases = [c for c in self.cases if c.error is not None]
        if len(error_cases) == self.total_count:
            return "ERROR"
        if error_cases:
            return "PARTIAL"
        return "FAIL" if self.pass_rate < self.threshold else "PASS"

    @property
    def exit_code(self) -> int:
        """Mapping from gate_result to CLI exit code."""
        mapping = {"PASS": 0, "FAIL": 1, "ERROR": 2, "PARTIAL": 3}
        return mapping.get(self.gate_result, 2)


@dataclass
class DatasetRow:
    """A single row in a TAG eval dataset."""
    id: str
    run_id: str            # source run_id from runs table; empty string for manual rows
    input: str
    reference_output: str
    created_at: str


@dataclass
class EvalDataset:
    """An in-memory representation of a dataset YAML."""
    name: str
    version: str
    created_at: str
    source_type: str                    # 'runs_table' | 'manual'
    source_profile: str | None
    source_since: str | None
    row_count: int
    path: Path
    rows: list[DatasetRow] = field(default_factory=list)
```

### 9.5 Core Algorithm: `run_eval_ci()`

```python
# src/tag/ci_eval.py

import sqlite3
import subprocess
import sys
import time
import datetime as dt
from pathlib import Path
from typing import Any


def run_eval_ci(
    cfg: dict[str, Any],
    conn: sqlite3.Connection,
    suite_path: Path,
    fail_below: float,
    profile: str,
    repo: str | None,
    pr_number: int | None,
    post_comment: bool,
    quiet: bool,
    timeout_seconds: int,
) -> CIEvalRunResult:
    """
    Orchestrate a full eval CI run.

    Steps:
      1. Load and validate suite YAML.
      2. Create eval_ci_runs row in SQLite (status: running).
      3. For each case: spawn agent, score, record to eval_ci_cases.
      4. Compute aggregate pass_rate and gate_result.
      5. Update eval_ci_runs row with final state.
      6. Optionally post PR comment.
      7. Return CIEvalRunResult.
    """
    from tag.eval_framework import load_suite, score_case
    from tag.ci import post_pr_comment

    suite = load_suite(suite_path)
    suite_name = suite.get("name", suite_path.stem)
    cases = suite.get("cases", [])
    suite_threshold = suite.get("threshold", fail_below)

    run_id = "ecr_" + uuid.uuid4().hex[:12]
    created_at = dt.datetime.utcnow().isoformat() + "Z"

    _insert_ci_run_row(conn, run_id, suite_path, suite_name, profile,
                       fail_below, created_at)

    results: list[CIEvalCaseResult] = []
    for case in cases:
        case_id = case.get("id", f"case_{len(results)+1}")
        case_threshold = case.get("threshold", suite_threshold)
        if not quiet:
            print(f"  Running: {case_id}", end="", flush=True)

        t0 = time.monotonic()
        output, error = _run_agent_case(cfg, profile, case.get("input", ""),
                                        timeout_seconds)
        duration_ms = int((time.monotonic() - t0) * 1000)

        if error:
            cr = CIEvalCaseResult(
                case_id=case_id,
                input=case.get("input", ""),
                output="",
                score=0.0,
                passed=False,
                threshold=case_threshold,
                failure_reason=None,
                error=error,
                duration_ms=duration_ms,
            )
        else:
            passed, score, reason = score_case(case, output)
            cr = CIEvalCaseResult(
                case_id=case_id,
                input=case.get("input", ""),
                output=output,
                score=score,
                passed=passed,
                threshold=case_threshold,
                failure_reason=reason,
                error=None,
                duration_ms=duration_ms,
            )

        _insert_ci_case_row(conn, run_id, cr, created_at)
        results.append(cr)

        if not quiet:
            status = "✓" if cr.passed else "✗"
            reason_str = f"  {cr.failure_reason}" if cr.failure_reason else ""
            print(f"\r  [{status}] {case_id:<40} score={cr.score:.2f}{reason_str}")

    run_result = CIEvalRunResult(
        eval_ci_run_id=run_id,
        suite_path=str(suite_path),
        suite_name=suite_name,
        profile=profile,
        threshold=fail_below,
        cases=results,
        pr_number=pr_number,
        repo=repo,
    )

    completed_at = dt.datetime.utcnow().isoformat() + "Z"
    _update_ci_run_row(conn, run_result, completed_at)

    if post_comment and repo and pr_number:
        body = build_pr_comment_body(run_result)
        ok = post_pr_comment(repo, pr_number, body)
        run_result.comment_posted = ok
        if not ok:
            print("Warning: failed to post PR comment via gh CLI.", file=sys.stderr)

    return run_result


def _run_agent_case(
    cfg: dict[str, Any],
    profile: str,
    prompt: str,
    timeout_seconds: int,
) -> tuple[str, str | None]:
    """
    Spawn a TAG agent run for a single eval case.

    Returns (output, error_message). error_message is None on success.
    Mirrors the subprocess pattern in cmd_eval (PRD-027).
    """
    from tag.controller import config_path, profile_exec_env

    try:
        result = subprocess.run(
            [
                sys.executable, "-m", "tag",
                "submit",
                "--task-type", "mixed",
                "--prompt", prompt,
                "--master-profile", profile,
                "--source", "eval_ci",
            ],
            env=profile_exec_env(cfg, profile),
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
        )
        return result.stdout.strip(), None
    except subprocess.TimeoutExpired:
        return "", f"timeout after {timeout_seconds}s"
    except Exception as exc:
        return "", f"subprocess error: {exc}"
```

### 9.6 PR Comment Builder

```python
# src/tag/ci_eval.py

_BADGE_PASS = "✅ PASS"
_BADGE_FAIL = "❌ FAIL"


def build_pr_comment_body(result: CIEvalRunResult) -> str:
    """
    Build the Markdown body for the PR comment.

    Includes:
      - Header summary table
      - Failed cases table (if any)
      - Full case table
      - Hidden JSON blob for machine parsing
    """
    import json

    gate_badge = _BADGE_PASS if result.gate_result == "PASS" else _BADGE_FAIL
    pass_pct = f"{result.pass_rate * 100:.1f}%"
    threshold_pct = f"{result.threshold * 100:.1f}%"

    lines = [
        "## TAG Eval CI Results",
        "",
        "| Suite | Cases | Passed | Failed | Pass Rate | Threshold | Status |",
        "|-------|-------|--------|--------|-----------|-----------|--------|",
        f"| `{result.suite_path}` | {result.total_count} | "
        f"{result.passed_count} | {result.failed_count} | "
        f"{pass_pct} | {threshold_pct} | {gate_badge} |",
        "",
    ]

    failed = [c for c in result.cases if not c.passed]
    if failed:
        lines += [
            "### Failed Cases",
            "",
            "| Case | Score | Reason |",
            "|------|-------|--------|",
        ]
        for c in failed:
            reason = (c.failure_reason or c.error or "").replace("|", "\\|")
            lines.append(f"| `{c.case_id}` | {c.score:.2f} | {reason} |")
        lines.append("")

    lines += [
        "### All Cases",
        "",
        "| Case | Score | Threshold | Status |",
        "|------|-------|-----------|--------|",
    ]
    for c in result.cases:
        status = "✅ pass" if c.passed else "❌ fail"
        lines.append(
            f"| `{c.case_id}` | {c.score:.2f} | {c.threshold:.2f} | {status} |"
        )

    json_blob = json.dumps({
        "eval_ci_run_id": result.eval_ci_run_id,
        "suite": result.suite_path,
        "profile": result.profile,
        "pass_rate": round(result.pass_rate, 4),
        "threshold": result.threshold,
        "passed": result.passed_count,
        "failed": result.failed_count,
        "total": result.total_count,
        "gate": result.gate_result,
    }, separators=(",", ":"))

    lines += [
        "",
        "---",
        f"*Generated by [tag-agent](https://github.com/tag-agent/tag) "
        f"`tag eval ci` · eval_ci_run_id: `{result.eval_ci_run_id}`*",
        "",
        f"<!-- tag-eval-result: {json_blob} -->",
    ]

    return "\n".join(lines)
```

### 9.7 Dataset Creation Algorithm

```python
# src/tag/ci_eval.py

import re


def parse_since_window(since: str) -> datetime.timedelta:
    """
    Parse a since-window string into a timedelta.

    Accepted formats: Nd (days), Nh (hours), Nw (weeks).
    Raises ValueError on invalid format.
    """
    match = re.fullmatch(r"(\d+)([dhw])", since.strip())
    if not match:
        raise ValueError(
            f"Invalid --since format: {since!r}. Use Nd, Nh, or Nw (e.g. 7d, 24h, 2w)."
        )
    n, unit = int(match.group(1)), match.group(2)
    if unit == "d":
        return datetime.timedelta(days=n)
    if unit == "h":
        return datetime.timedelta(hours=n)
    return datetime.timedelta(weeks=n)


def _jaccard_similarity(a: str, b: str) -> float:
    """Token-level Jaccard similarity between two strings."""
    tokens_a = set(re.split(r"[\s\W]+", a.lower()))
    tokens_b = set(re.split(r"[\s\W]+", b.lower()))
    tokens_a.discard("")
    tokens_b.discard("")
    if not tokens_a and not tokens_b:
        return 1.0
    intersection = len(tokens_a & tokens_b)
    union = len(tokens_a | tokens_b)
    return intersection / union if union > 0 else 0.0


def create_dataset_from_runs(
    conn: sqlite3.Connection,
    name: str,
    since: str,
    limit: int,
    profile: str | None,
    min_output_length: int,
    output_path: Path,
) -> EvalDataset:
    """
    Mine the runs/steps tables and write a dataset YAML.

    Algorithm:
      1. Query runs WHERE created_at >= cutoff AND status = 'done'
         AND (master_profile = profile OR profile IS NULL).
      2. For each run, get the last 'assistant' step output.
      3. Filter by min_output_length.
      4. Sort by created_at ASC, run_id ASC (deterministic).
      5. Deduplicate by Jaccard similarity >= 0.85 (keep first occurrence).
      6. Apply limit.
      7. Write dataset YAML.
      8. Upsert eval_datasets row.
    """
    cutoff = (datetime.datetime.utcnow() - parse_since_window(since)).isoformat() + "Z"

    profile_clause = "AND r.master_profile = ?" if profile else ""
    params: list[Any] = [cutoff]
    if profile:
        params.append(profile)

    rows = conn.execute(
        f"""
        SELECT r.id AS run_id,
               r.prompt AS input,
               r.created_at,
               s.output AS reference_output
        FROM runs r
        JOIN steps s ON s.run_id = r.id
                    AND s.role = 'assistant'
        WHERE r.created_at >= ?
          AND r.status = 'done'
          {profile_clause}
          AND length(s.output) >= ?
        GROUP BY r.id               -- one row per run (last assistant step via MAX rowid)
        HAVING s.id = MAX(s.id)
        ORDER BY r.created_at ASC, r.id ASC
        """,
        params + [min_output_length],
    ).fetchall()

    # Deduplication
    deduped: list[sqlite3.Row] = []
    for row in rows:
        is_dup = any(
            _jaccard_similarity(row["input"], kept["input"]) >= 0.85
            for kept in deduped
        )
        if not is_dup:
            deduped.append(row)

    selected = deduped[:limit]

    dataset_rows = [
        DatasetRow(
            id=f"row_{i+1:03d}",
            run_id=r["run_id"],
            input=r["input"],
            reference_output=r["reference_output"],
            created_at=r["created_at"],
        )
        for i, r in enumerate(selected)
    ]

    created_at_str = datetime.datetime.utcnow().isoformat() + "Z"
    dataset = EvalDataset(
        name=name,
        version="1.0.0",
        created_at=created_at_str,
        source_type="runs_table",
        source_profile=profile,
        source_since=since,
        row_count=len(dataset_rows),
        path=output_path,
        rows=dataset_rows,
    )

    _write_dataset_yaml(dataset, output_path)
    _upsert_eval_dataset_row(conn, dataset)

    return dataset
```

### 9.8 `cmd_eval_ci` in `controller.py`

```python
# src/tag/controller.py  (new function, follows existing cmd_eval pattern)

def cmd_eval_ci(args: argparse.Namespace) -> int:
    """PRD-047: Eval CI gate with PR comment integration."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    conn = open_db(cfg)

    try:
        from tag.ci_eval import run_eval_ci, format_results_json
        from tag.eval_framework import ensure_schema as ensure_eval_schema
    except ImportError as exc:
        conn.close()
        print_error(f"tag.ci_eval not available: {exc}")
        return 2

    suite_path_str = getattr(args, "suite", None)
    if not suite_path_str:
        conn.close()
        print_error("--suite SUITE_PATH is required")
        return 2

    suite_path = Path(suite_path_str)
    if not suite_path.exists():
        # Try ~/.tag/evals/ fallback
        fallback = Path.home() / ".tag" / "evals" / suite_path.name
        if fallback.exists():
            suite_path = fallback
        else:
            conn.close()
            print_error(f"Suite not found: {suite_path}")
            return 2

    fail_below = float(getattr(args, "fail_below", 0.0))
    profile = getattr(args, "profile", None) or cfg.get("defaults", {}).get("master_profile", "orchestrator")
    post_comment = getattr(args, "post_comment", False)
    repo = getattr(args, "repo", None)
    pr_number = getattr(args, "pr", None)
    quiet = getattr(args, "quiet", False)
    timeout_seconds = int(getattr(args, "timeout", 300))
    emit_json = getattr(args, "json", False)

    if post_comment and (not repo or not pr_number):
        conn.close()
        print_error("--post-comment requires both --repo and --pr")
        return 2

    # Disable comment posting if not in PR context (safety check for branch runs)
    if post_comment and not pr_number:
        print_warning("--post-comment ignored: no --pr number provided")
        post_comment = False

    if not quiet:
        print(f"Eval CI: {suite_path}  profile={profile}  threshold={fail_below:.2f}")

    result = run_eval_ci(
        cfg=cfg,
        conn=conn,
        suite_path=suite_path,
        fail_below=fail_below,
        profile=profile,
        repo=repo,
        pr_number=pr_number,
        post_comment=post_comment,
        quiet=quiet,
        timeout_seconds=timeout_seconds,
    )
    conn.close()

    if not quiet:
        pass_pct = f"{result.pass_rate * 100:.1f}%"
        threshold_pct = f"{result.threshold * 100:.1f}%"
        print(f"\nResults: {result.passed_count}/{result.total_count} passed  "
              f"pass_rate={pass_pct}  threshold={threshold_pct}")
        gate_msg = f"GATE: {result.gate_result}"
        if result.gate_result == "FAIL":
            print_error(f"{gate_msg} — pass rate {pass_pct} is below threshold {threshold_pct}")
        elif result.gate_result == "PASS":
            print_success(gate_msg)
        elif result.gate_result == "PARTIAL":
            print_warning(f"{gate_msg} — some cases errored; pass rate computed over completed cases")
        elif result.gate_result == "ERROR":
            print_error(f"{gate_msg} — all cases failed to run")

    if emit_json:
        import json
        print(json.dumps(format_results_json(result), indent=2))

    return result.exit_code
```

### 9.9 `cmd_ci` Extension for `install-action`

The existing `cmd_ci` in `controller.py` handles `diagnose`, `commit-lint`, and `status`. Extend it with an `install-action` branch:

```python
# Inside cmd_ci(), after the existing subcommand checks:

if sub == "install-action":
    action_type = getattr(args, "type", None)
    if action_type != "eval":
        print_error(f"Unknown --type: {action_type}. Supported: eval")
        return 1

    try:
        from tag.ci_eval import scaffold_eval_workflow
    except ImportError as exc:
        print_error(f"tag.ci_eval not available: {exc}")
        return 1

    output_path = Path(getattr(args, "output", ".github/workflows/tag-eval.yml"))
    suite = getattr(args, "suite", "evals/golden.yaml")
    fail_below = float(getattr(args, "fail_below", 0.85))
    profile_name = getattr(args, "profile", "coder")
    force = getattr(args, "force", False)

    return scaffold_eval_workflow(
        output_path=output_path,
        suite=suite,
        fail_below=fail_below,
        profile=profile_name,
        force=force,
    )
```

### 9.10 Integration Points

| Integration Point | Module | How Used |
|-------------------|--------|----------|
| `eval_framework.load_suite()` | `src/tag/eval_framework.py` | Loads and validates the suite YAML before any case runs |
| `eval_framework.score_case()` | `src/tag/eval_framework.py` | Scores each agent output against the case's keyword/pattern criteria |
| `ci.post_pr_comment()` | `src/tag/ci.py` | Posts the Markdown comment body to the GitHub PR |
| `open_db()` | `src/tag/controller.py` | Returns WAL-mode SQLite connection; DDL created on first access |
| `profile_exec_env()` | `src/tag/controller.py` | Resolves model API keys and environment from the named profile |
| `ensure_runtime_dirs()` | `src/tag/controller.py` | Ensures `~/.tag/runtime/` exists before SQLite write |
| `ThreadPoolExecutor` | stdlib | Used internally for parallel case runs (future: `--parallel N` flag, not in v1) |

---

## 10. Security Considerations

1. **`GH_TOKEN` scope minimization:** The scaffolded `tag-eval.yml` explicitly sets `permissions: pull-requests: write, contents: read` at the job level, not the workflow level. This restricts the `GITHUB_TOKEN` to the minimum scope required for `gh pr comment`. The workflow YAML includes a comment explaining why write access to contents is read-only, and why no other permissions (packages, secrets, deployments) are granted.

2. **Suite YAML path traversal:** `--suite` is resolved relative to cwd, then `~/.tag/evals/`. Resolved absolute paths containing `..` components that escape these roots are rejected with exit 2. This prevents a malicious `--suite ../../etc/passwd` from being loaded as a YAML file (though YAML parsing would fail anyway, the rejection provides defense-in-depth).

3. **Prompt injection via `input` field:** Suite YAML `input` fields are passed verbatim as prompts to the agent subprocess. A malicious YAML file could encode a jailbreak or system-prompt injection attempt in a case's `input` field. Mitigations: (a) eval suites should be code-reviewed like any other source file before being committed; (b) `tag eval ci` runs the agent via the profile's normal sandbox (PRD-028), which enforces tool grants; (c) no `input` field is ever echoed into the CI workflow YAML itself — it only enters the agent's context, not the Actions runner configuration.

4. **PR comment injection:** The Markdown body is constructed from controlled fields (`suite_path`, `profile`, `case_id`, `score`, `failure_reason`). Any pipe characters in `failure_reason` or `case_id` are escaped to `\|` before being embedded in a Markdown table cell. The hidden JSON blob uses `json.dumps()` with `separators=(",",":")` which is safe for embedding in an HTML comment (no `-->` sequence can appear in valid JSON).

5. **Secret leakage in `output` field:** Agent outputs stored in `eval_ci_cases.output` may contain fragments of files the agent read during task execution. If a profile has file-read tool access and a case's `input` prompts reading a secrets file, the output (and the PR comment) could contain secret material. Mitigation: eval profiles should use restricted tool grants (no `read_file` on sensitive paths, or no file-read tools at all). Document this in the `tag ci install-action` output and in the generated workflow YAML comment.

6. **SQLite write atomicity:** Each `eval_ci_cases` row is inserted in its own `BEGIN IMMEDIATE` transaction. If the process is killed mid-run, partial results are visible in the database with `eval_ci_run_id` matching the in-progress run. The `eval_ci_runs.completed_at` field remains NULL for killed runs, enabling cleanup queries: `DELETE FROM eval_ci_cases WHERE eval_ci_run_id IN (SELECT id FROM eval_ci_runs WHERE completed_at IS NULL AND created_at < datetime('now','-1 hour'))`.

7. **API key exposure in GitHub Actions logs:** The model API key (`ANTHROPIC_API_KEY`) is passed to the agent subprocess via the environment, inherited from the Actions step's `env:` block. GitHub Actions automatically masks values of secrets in log output. However, if the agent output contains the key (e.g., the agent was asked "what is your API key?"), GitHub's masking would also redact it from the PR comment — causing the comment to display `***` in place of the key. This is the desired behavior.

8. **Rate limiting of `post_pr_comment` on large PR stacks:** In monorepo environments where hundreds of PRs are opened simultaneously, multiple `tag eval ci --post-comment` runs could hit GitHub API rate limits. The `gh pr comment` command uses the authenticated user's rate limit bucket (5000 requests/hour for PAT, unlimited for `GITHUB_TOKEN` within the repo). At 1 comment per eval CI run, this is not a practical concern.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_eval_ci.py`)

- **`build_pr_comment_body`:** Given a `CIEvalRunResult` with known pass/fail data, assert the output Markdown contains the correct table rows, gate badge, hidden JSON blob, and proper pipe escaping for `failure_reason` values containing `|`.
- **`parse_since_window`:** Parameterized tests: `"7d"` → `timedelta(days=7)`, `"24h"` → `timedelta(hours=24)`, `"2w"` → `timedelta(weeks=2)`, `""` → `ValueError`, `"7x"` → `ValueError`, `"abc"` → `ValueError`.
- **`_jaccard_similarity`:** Test: identical strings → 1.0, empty strings → 1.0, completely disjoint → 0.0, partial overlap → expected float.
- **`CIEvalRunResult.gate_result`:** Mock `cases` list; test all four gate states: all-pass → "PASS", below-threshold → "FAIL", some-errors → "PARTIAL", all-errors → "ERROR".
- **`CIEvalRunResult.exit_code`:** Verify mapping: PASS=0, FAIL=1, ERROR=2, PARTIAL=3.
- **Threshold boundary:** Tests at `pass_rate = fail_below - 0.001` → FAIL, `pass_rate = fail_below` → PASS, `pass_rate = fail_below + 0.001` → PASS.
- **`scaffold_eval_workflow`:** Mock `Path.write_text`; verify the generated YAML contains `--fail-below`, `--suite`, and `--profile` values injected from args. Verify `--force` flag behavior.

### 11.2 Integration Tests (`tests/test_eval_ci_integration.py`)

- **End-to-end with mocked agent subprocess:** Patch `subprocess.run` to return controlled stdout for each case. Verify `eval_ci_runs` and `eval_ci_cases` rows are written to an in-memory SQLite DB with correct values.
- **PR comment posting:** Patch `ci.post_pr_comment` to return `True`/`False`; verify `CIEvalRunResult.comment_posted` reflects the return value and that the exit code is independent of comment success.
- **Timeout handling:** Patch `subprocess.run` to raise `subprocess.TimeoutExpired`; verify the case is recorded with `error="timeout after Xs"` and `passed=False`.
- **`tag eval dataset create` with seeded DB:** Insert 20 `runs` rows and corresponding `steps` rows into a test DB; verify `create_dataset_from_runs` produces a dataset with correct row count after deduplication and min-length filtering.
- **Path traversal rejection:** Verify `cmd_eval_ci` with `--suite ../../etc/anything` exits 2 without loading the file.

### 11.3 Performance Tests

- **Dataset creation at scale:** Seed 10,000 `runs` rows; measure `create_dataset_from_runs` wall time with `--since 30d --limit 50`. Target: under 5 seconds on a developer laptop with WAL-mode SQLite.
- **`eval_ci_cases` insert throughput:** Insert 100 `eval_ci_cases` rows serially; verify total time under 1 second (each row is a trivial INSERT; the bottleneck should be the agent subprocess, not SQLite).

### 11.4 CI Smoke Test

A GitHub Actions workflow in the `tag-agent` repo itself runs:

```bash
tag eval ci \
  --suite src/tag/evals/smoke.yaml \
  --fail-below 0.0 \
  --profile passthrough \
  --quiet
```

The `passthrough` profile echoes input to output without making a model API call, and the `smoke.yaml` suite uses `expect_contains` patterns that always match the echoed input. This tests the full code path (subprocess spawn, scoring, SQLite write, exit code) without requiring a real API key or incurring cost. This smoke test runs on every PR to `tag-agent` itself.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag eval ci --suite evals/golden.yaml --fail-below 0.85 --profile coder` exits 1 when `pass_rate < 0.85` and exits 0 when `pass_rate >= 0.85`. | Parameterized unit test with mocked `score_case()` returning controlled scores. |
| AC-02 | When `--fail-below` is not provided, `tag eval ci` exits 0 regardless of pass rate (gate is disabled). | Unit test: all cases fail; no `--fail-below`; assert exit code 0. |
| AC-03 | A run of `tag eval ci` writes exactly one row to `eval_ci_runs` and exactly N rows to `eval_ci_cases` (one per case). | Integration test with in-memory SQLite; assert row counts. |
| AC-04 | The `eval_ci_runs.gate_result` column contains `'FAIL'` when `pass_rate < threshold`, `'PASS'` otherwise. | Integration test. |
| AC-05 | `--post-comment` calls `ci.post_pr_comment` exactly once with the correct `repo` and `pr_number`. | Integration test: mock `post_pr_comment`; assert call count and args. |
| AC-06 | The PR comment body contains a `<!-- tag-eval-result: {...} -->` HTML comment on the last non-empty line, and the embedded JSON is valid and parseable. | Unit test: `json.loads()` the extracted blob from `build_pr_comment_body()` output. |
| AC-07 | `tag ci install-action --type eval` creates `.github/workflows/tag-eval.yml` with `permissions: pull-requests: write` and `contents: read`. | Integration test: read the generated YAML; assert permissions block. |
| AC-08 | `tag ci install-action --type eval` exits 2 with an error message if the output file already exists and `--force` is not passed. | Unit test with pre-existing file. |
| AC-09 | `tag ci install-action --type eval --force` overwrites an existing file without prompting. | Unit test. |
| AC-10 | `tag eval dataset create my-golden --from-runs --since 7d --limit 50` creates `evals/datasets/my-golden.yaml` with at most 50 rows, each row having non-empty `input` and `reference_output` fields. | Integration test with seeded SQLite DB. |
| AC-11 | `tag eval dataset create` writes an `eval_datasets` row to SQLite with the correct `name`, `row_count`, `source_type`, and `source_since` fields. | Integration test: query SQLite after create; assert column values. |
| AC-12 | `tag eval dataset list --json` returns a JSON array where each element has `name`, `path`, `row_count`, `source_type`, and `created_at` fields. | Unit test: seed `eval_datasets` table; assert JSON schema. |
| AC-13 | A timed-out case is recorded in `eval_ci_cases` with `passed=0`, `score=0.0`, and `error` containing `"timeout"`. The overall exit code is 3 (PARTIAL) when at least one case errors and at least one completes. | Integration test: mock first case to timeout, second to succeed. |
| AC-14 | `parse_since_window("7d")` returns `timedelta(days=7)`. `parse_since_window("bad")` raises `ValueError` with a message containing `"Nd, Nh, or Nw"`. | Unit test. |
| AC-15 | `--post-comment` with a missing or unauthenticated `gh` CLI prints a warning to stderr but does not change the exit code from the gate decision. | Integration test: mock `post_pr_comment` to return `False`; assert exit code is determined by threshold alone. |
| AC-16 | The generated `tag-eval.yml` embeds the `--fail-below`, `--suite`, and `--profile` values passed to `tag ci install-action`. | Unit test: parse generated YAML; assert `run:` step string contains the flag values. |

---

## 13. Dependencies

| Dependency | Type | Justification |
|------------|------|---------------|
| PRD-027 eval framework | Internal (existing) | `eval_framework.load_suite()` and `score_case()` are the scoring engine. No changes to PRD-027's API. |
| PRD-020 CI/CD integration | Internal (existing) | `ci.post_pr_comment()` is used as-is for the PR comment posting step. |
| PRD-013 agent tracing | Internal (existing) | `open_db()` / `ensure_runtime_dirs()` / WAL-mode SQLite setup is inherited. |
| PRD-028 sandbox | Internal (existing) | Agent subprocess runs in the normal sandbox defined by the profile's tool grants. |
| PRD-034 secret scanning | Internal (existing) | Secret pattern detection should extend to `eval_ci_cases.output` column in a future PRD; noted as OQ-4. |
| `gh` CLI | External tool | Required only for `--post-comment`. Not required for gate-only mode. Must be authenticated via `GH_TOKEN` or `gh auth login`. |
| `PyYAML` | Python package | Already a required dependency of `tag-agent`. Used by `eval_framework.load_suite()` and `_write_dataset_yaml()`. |
| `ruamel.yaml` | Python package (optional) | Only needed if `tag eval dataset create` needs to emit round-trip-safe YAML with comments. Initial implementation uses `PyYAML` for simplicity; `ruamel.yaml` may be added in a follow-up. |
| `eval_framework.ensure_schema()` | Internal function | Creates `eval_runs` / `eval_cases` tables. `ci_eval.py` similarly creates `eval_ci_runs` / `eval_ci_cases` / `eval_datasets` via its own `ensure_ci_schema()` called at DB open time. |
| `subprocess` stdlib | stdlib | Agent subprocess execution per case. |
| `concurrent.futures` stdlib | stdlib | Reserved for future `--parallel N` support; not used in v1. |

---

## 14. Open Questions

| ID | Question | Impact | Owner | Status |
|----|----------|--------|-------|--------|
| OQ-01 | Should `tag eval ci` update an existing PR comment (via `gh api --method PATCH`) instead of posting a new one on each CI re-run? Updating requires storing the comment ID from the first post in `eval_ci_runs`. The benefit is a cleaner PR timeline. The risk is that updating requires the `gh` CLI to support comment ID lookup, which adds complexity. | UX | Product | Open — post-new-comment for v1; update behavior deferred to v2. |
| OQ-02 | Should `--fail-below` default to `0.0` (gate disabled unless explicit) or to `0.85` (opinionated default)? The `0.0` default makes `--fail-below` effectively required for gating, which is safer but less ergonomic. The `0.85` default makes the gate "on by default" which could surprise teams. | UX, adoption | Product | Proposed: `0.0` (disabled) as default; teams opt in explicitly. |
| OQ-03 | Should `tag eval dataset create` support `--from-file <path>` (import from a CSV or JSONL of (input, output) pairs) in addition to `--from-runs`? This would let teams import datasets from external benchmark sets (e.g., HumanEval). | Feature scope | Product | Open — `--from-runs` only in v1; `--from-file` deferred. |
| OQ-04 | Should `eval_ci_cases.output` be secret-scanned before being stored and before being included in PR comments? A pattern-matching scan (similar to PRD-034) could redact obvious secrets before they land in the database or in a public PR comment. | Security | Engineering | Open — not in scope for v1; tracked for PRD-034 extension. |
| OQ-05 | Should `tag eval dataset list` scan the filesystem (`evals/datasets/*.yaml`) on every invocation, or rely solely on the `eval_datasets` SQLite table? Filesystem scanning is always current but slower. SQLite table is fast but may be stale if YAML files are added/deleted manually outside of `tag`. | Performance, correctness | Engineering | Proposed: always scan filesystem and upsert `eval_datasets` on `list`; trade latency for freshness. |
| OQ-06 | What is the correct behavior when `--pr` is provided but the PR does not exist in `--repo`? Currently `post_pr_comment` returns `False` and a warning is printed. Should the gate exit 2 (error) instead? | Error handling | Engineering | Open — warning-only for v1; fail-hard in v2 based on user feedback. |
| OQ-07 | Should the generated `tag-eval.yml` pin the `tag-agent` version (`pip install tag-agent==0.3.0`) or install the latest (`pip install tag-agent`)? Pinning prevents surprise breakage from future `tag eval ci` API changes; latest is simpler for teams that want automatic updates. | Reliability | Product | Proposed: pin to the version that ran `tag ci install-action`, detectable from `tag.__version__`. |

---

## 15. Complexity and Timeline

**Overall Effort:** S (3–5 engineering days)

| Phase | Tasks | Days |
|-------|-------|------|
| **Phase 1: Core gate** (Days 1–2) | `src/tag/ci_eval.py`: `CIEvalCaseResult`, `CIEvalRunResult`, `run_eval_ci()`, `_run_agent_case()`, SQLite DDL (`eval_ci_runs`, `eval_ci_cases`, `eval_datasets`), `ensure_ci_schema()`. Wire `cmd_eval_ci()` in `controller.py`. Argparse subparser for `tag eval ci`. Unit tests for gate logic and threshold boundary. | 2 |
| **Phase 2: PR comment + install-action** (Day 3) | `build_pr_comment_body()` with hidden JSON blob. `scaffold_eval_workflow()`. `cmd_ci install-action` branch in `controller.py`. `format_results_json()`. Integration test for comment posting with mocked `post_pr_comment`. Unit test for generated YAML content. | 1 |
| **Phase 3: Dataset commands** (Day 4) | `parse_since_window()`, `_jaccard_similarity()`, `create_dataset_from_runs()`, `_write_dataset_yaml()`, `list_datasets()`. `cmd_eval_dataset()` in `controller.py` with `create` and `list` subcommands. Argparse subparsers. Integration test with seeded SQLite DB. Performance test (10K rows). | 1 |
| **Phase 4: Polish + smoke test** (Day 5) | CI smoke test workflow (`tag eval ci` with `passthrough` profile). `--json` output for all commands. Path traversal rejection. `--quiet` flag. `--force` flag for `install-action`. Error message review. Acceptance criteria verification. | 1 |

**Risk Register:**

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `eval_framework.score_case()` API changes between PRD-027 and this PRD | Low | Medium | Pin the import to the current signature; add a compatibility check at import time. |
| `gh pr comment` authentication fails in CI (missing `GITHUB_TOKEN`) | Medium | Low | `post_pr_comment` already returns `False` on failure; gate decision is independent. |
| Large `runs` table (100K+ rows) makes `create_dataset_from_runs` slow | Low | Medium | The SQL query uses `created_at >= ?` which benefits from the existing `idx_er_status` index on `eval_runs`; add a dedicated index on `runs.created_at` if needed. |
| Deduplication O(n²) cost for large candidate sets | Low | Low | At 200 candidates (10× the typical `--limit`), O(n²) Jaccard is 40,000 comparisons; negligible on modern hardware. If this becomes a bottleneck, switch to MinHash LSH. |

